//! The step limit and "Continue?": what a host does when an agent run
//! stops because it used its model steps (`LlmAgentBuilder::max_iterations`).
//!
//! The run ends with adk's own end reason, [`reached`]; it is never inferred
//! from a step count. The host may then ask the person, through its
//! [`ApprovalChannel`] and never through rules or remembered choices, whether
//! to go on ([`continue_request`]): the question offers no "always" action,
//! so no answer is remembered, and every ask is a fresh question. On "yes"
//! the same runner runs again on the same session with the host-authored
//! [`CONTINUE_PROMPT`] ([`StepBudget::continue_content`]), at most
//! [`MAX_CONTINUES`] times per turn. A channel that cannot answer inline
//! (the cloud's, which defers) means "stop".

use std::time::Duration;

use adk_core::{AdkError, Content, ErrorComponent};
use serde_json::json;

use crate::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest};

/// How many times one turn may continue past its limit: the turn runs at
/// most `(MAX_CONTINUES + 1) × limit` model steps.
pub const MAX_CONTINUES: u32 = 3;

/// The host-authored user content a continued run starts from. Not the
/// person's message: hosts mark it as host-authored wherever they show it.
pub const CONTINUE_PROMPT: &str = "Continue from where you stopped.";

/// The approval payload key of the question (its value: `steps`,
/// `detail`).
pub const CONTINUE_PAYLOAD_KEY: &str = "step_limit_continue";

/// The interaction kind hosts record the question under.
pub const CONTINUE_KIND: &str = "continue";

/// How long the question waits for an answer; unanswered, the turn stops.
pub const CONTINUE_TIMEOUT: Duration = Duration::from_mins(10);

/// The note a turn that stopped at its limit carries.
pub const STEP_LIMIT_REACHED: &str = "step_limit_reached";

/// How much of the agent's last text the question shows.
pub const DETAIL_CHARS: usize = 300;

/// The message adk 2.2's agent loop ends a run with when `max_iterations`
/// is used up (`adk-agent/src/llm_agent.rs`; the tripwire test below fails
/// if an adk upgrade changes it).
const MAX_ITERATIONS_PREFIX: &str = "Max iterations (";

/// Whether `error` is adk's "the run used its model steps" end, rather than
/// a failure.
#[must_use]
pub fn reached(error: &AdkError) -> bool {
    error.component == ErrorComponent::Agent
        && error.message.starts_with(MAX_ITERATIONS_PREFIX)
        && error.message.ends_with(") exceeded")
}

/// One turn's steps: the limit per run and the continues used.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StepBudget {
    limit: u32,
    continues: u32,
}

impl StepBudget {
    #[must_use]
    pub const fn new(limit: u32) -> Self {
        Self {
            limit,
            continues: 0,
        }
    }

    /// The steps of each run.
    #[must_use]
    pub const fn limit(&self) -> u32 {
        self.limit
    }

    /// The continues used so far.
    #[must_use]
    pub const fn continues(&self) -> u32 {
        self.continues
    }

    /// Whether the person may still be asked to continue.
    #[must_use]
    pub const fn may_continue(&self) -> bool {
        self.continues < MAX_CONTINUES
    }

    /// Count one continue and give the content the next run starts from;
    /// `None` (and nothing counted) once [`MAX_CONTINUES`] are used.
    pub fn continue_content(&mut self) -> Option<Content> {
        if !self.may_continue() {
            return None;
        }
        self.continues += 1;
        Some(Content::new("user").with_text(CONTINUE_PROMPT))
    }
}

/// The question: "The agent used its {steps} steps", with the start of
/// its last text. Offers `approve` (continue for `steps` more) and `reject`
/// (stop) only: nothing to remember.
#[must_use]
pub fn continue_request(subject: &str, steps: u32, last_text: &str) -> ApprovalRequest {
    let mut detail: String = last_text.trim().chars().take(DETAIL_CHARS).collect();
    if last_text.trim().chars().count() > DETAIL_CHARS {
        detail.push('…');
    }
    ApprovalRequest {
        subject: subject.to_owned(),
        message: format!("The agent used its {steps} steps"),
        available_actions: vec!["approve".to_owned(), "reject".to_owned()],
        payload: json!({ CONTINUE_PAYLOAD_KEY: { "steps": steps, "detail": detail } }),
    }
}

/// Ask `channel` whether to continue; `false` (stop) on "reject", on a
/// deferred or failed question, and after [`CONTINUE_TIMEOUT`].
pub async fn ask_to_continue(
    channel: &dyn ApprovalChannel,
    subject: &str,
    steps: u32,
    last_text: &str,
) -> bool {
    let question = channel.request(continue_request(subject, steps, last_text));
    matches!(
        tokio::time::timeout(CONTINUE_TIMEOUT, question).await,
        Ok(Ok(ApprovalOutcome::Decided { action, .. })) if action == "approve"
    )
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use adk_agent::LlmAgentBuilder;
    use adk_core::{Llm, LlmRequest, LlmResponse, LlmResponseStream, Part, Tool, ToolContext};
    use adk_runner::Runner;
    use adk_session::{CreateRequest, InMemorySessionService, SessionService};
    use async_trait::async_trait;
    use futures::StreamExt as _;
    use serde_json::Value;

    use super::*;
    use crate::host::HostError;

    /// A model that always calls `look`.
    #[derive(Default)]
    struct Looping(AtomicUsize);

    #[async_trait]
    impl Llm for Looping {
        fn name(&self) -> &'static str {
            "looping"
        }

        async fn generate_content(
            &self,
            _request: LlmRequest,
            _stream: bool,
        ) -> adk_core::Result<LlmResponseStream> {
            let call = self.0.fetch_add(1, Ordering::SeqCst);
            let mut content = Content::new("model");
            content.parts.push(Part::FunctionCall {
                name: "look".to_owned(),
                args: json!({}),
                id: Some(format!("call-{call}")),
                thought_signature: None,
            });
            let response = LlmResponse {
                content: Some(content),
                turn_complete: true,
                ..LlmResponse::default()
            };
            Ok(Box::pin(futures::stream::iter([Ok(response)])))
        }
    }

    struct Look;

    #[async_trait]
    impl Tool for Look {
        fn name(&self) -> &'static str {
            "look"
        }

        fn description(&self) -> &'static str {
            "Look around."
        }

        async fn execute(
            &self,
            _ctx: Arc<dyn ToolContext>,
            _args: Value,
        ) -> adk_core::Result<Value> {
            Ok(json!({"seen": true}))
        }
    }

    /// The tripwire: adk's own end of a run that used its steps is what
    /// [`reached`] recognises, and a continued run on the same session goes
    /// on from it.
    #[tokio::test]
    async fn the_step_limit_end_is_adks_own_and_a_continue_resumes_the_session() {
        let model = Arc::new(Looping::default());
        let agent = LlmAgentBuilder::new("agent")
            .model(model.clone())
            .tool(Arc::new(Look))
            .max_iterations(2)
            .build()
            .unwrap();
        let sessions = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "app".to_owned(),
                user_id: "user".to_owned(),
                session_id: Some("s".to_owned()),
                state: HashMap::new(),
            })
            .await
            .unwrap();
        let runner = Runner::builder()
            .app_name("app")
            .agent(Arc::new(agent))
            .session_service(sessions.clone())
            .build()
            .unwrap();
        let mut budget = StepBudget::new(2);
        let mut content = Content::new("user").with_text("go");
        let mut ends = Vec::new();
        loop {
            let mut stream = runner.run_str("user", "s", content).await.unwrap();
            let mut end = None;
            while let Some(event) = stream.next().await {
                if let Err(error) = event {
                    end = Some(error);
                }
            }
            let error = end.expect("the run ends at its limit");
            ends.push(reached(&error));
            match budget.continue_content() {
                Some(next) => content = next,
                None => break,
            }
        }
        assert_eq!(ends, [true; 4]);
        assert_eq!(model.0.load(Ordering::SeqCst), 8, "4 runs of 2 steps");
        assert_eq!(budget.continues(), MAX_CONTINUES);
        assert!(!budget.may_continue());
        // One session: every run's events, and the host's prompt three times.
        let session = sessions
            .get(adk_session::GetRequest {
                app_name: "app".to_owned(),
                user_id: "user".to_owned(),
                session_id: "s".to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .unwrap();
        let prompts = session
            .events()
            .all()
            .iter()
            .filter(|event| {
                event.author == "user"
                    && event
                        .llm_response
                        .content
                        .as_ref()
                        .is_some_and(|c| matches!(&c.parts[..], [Part::Text { text }] if text == CONTINUE_PROMPT))
            })
            .count();
        assert_eq!(prompts, 3);

        assert!(!reached(&AdkError::agent("tool failed")));
        assert!(!reached(&AdkError::model("Max iterations (2) exceeded")));
    }

    struct Answer(Result<ApprovalOutcome, ()>);

    #[async_trait]
    impl ApprovalChannel for Answer {
        async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
            assert_eq!(request.available_actions, ["approve", "reject"]);
            self.0
                .clone()
                .map_err(|()| HostError::new(crate::host::HostErrorCode::ExecutionEnded, "ended"))
        }
    }

    #[tokio::test]
    async fn only_an_approval_continues() {
        let decided = |action: &str| {
            Answer(Ok(ApprovalOutcome::Decided {
                action: action.to_owned(),
                value: json!({}),
            }))
        };
        assert!(ask_to_continue(&decided("approve"), "t", 100, "").await);
        // "Always" is not offered; were it answered, it is not a continue.
        for channel in [
            decided("reject"),
            decided("approve_always"),
            Answer(Ok(ApprovalOutcome::Deferred)),
            Answer(Err(())),
        ] {
            assert!(!ask_to_continue(&channel, "t", 100, "").await);
        }
    }

    #[test]
    fn the_question_shows_the_start_of_the_last_text() {
        let request = continue_request("turn", 100, &"x".repeat(400));
        assert_eq!(request.message, "The agent used its 100 steps");
        let detail = request.payload[CONTINUE_PAYLOAD_KEY]["detail"]
            .as_str()
            .unwrap();
        assert_eq!(detail.chars().count(), DETAIL_CHARS + 1);
        assert_eq!(request.payload[CONTINUE_PAYLOAD_KEY]["steps"], 100);
        let short = continue_request("turn", 30, "  Fixed two tests.\n");
        assert_eq!(
            short.payload[CONTINUE_PAYLOAD_KEY]["detail"],
            "Fixed two tests."
        );
    }
}
