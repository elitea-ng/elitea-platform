//! Runtime integration tests for the Gemini Interactions API transport.
//!
//! These tests drive a real `Runner` + `LlmAgent` + `GeminiModel` configured
//! with `use_interactions_api(true)` against the live Interactions API. They
//! are `#[ignore]`d because they require:
//!   - `GOOGLE_API_KEY` (or `GEMINI_API_KEY`) in the environment,
//!   - network access, and
//!   - Interactions API (Beta) access for the target model.
//!
//! Run manually with:
//! ```bash
//! cargo test -p adk-model --features gemini-interactions \
//!     --test interactions_runtime_integration_tests -- --ignored
//! ```
//!
//! Compiled only when the `gemini-interactions` feature is enabled (enforced
//! via `required-features` on the test target in `Cargo.toml`).
//!
//! Mirrors ADK-Python `interactions_api` sample Test 1 (single-turn model
//! interaction): asserts the agent produces text **and** that at least one
//! emitted `Event` carries a populated `interaction_id`.
//!
//! Validates: Requirements 1.3, 4.1, 4.3.

use std::sync::Arc;

use adk_agent::LlmAgentBuilder;
use adk_core::{Agent, Content, Part, SessionId, UserId};
use adk_model::GeminiModel;
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use futures::StreamExt;

const APP_NAME: &str = "interactions-runtime-it";
const SESSION_ID: &str = "interactions-single-turn";
const MULTI_TURN_SESSION_ID: &str = "interactions-multi-turn";
const USER_ID: &str = "user";

/// Reads the Gemini API key from the environment, preferring `GOOGLE_API_KEY`
/// and falling back to `GEMINI_API_KEY`.
fn api_key() -> Option<String> {
    std::env::var("GOOGLE_API_KEY")
        .ok()
        .or_else(|| std::env::var("GEMINI_API_KEY").ok())
        .filter(|key| !key.trim().is_empty())
}

/// ADK-Python parity Test 1: single-turn model interaction via the runner.
///
/// Builds a `GeminiModel` with the Interactions transport enabled, wraps it in
/// a normal `LlmAgent`, drives it through a `Runner`, and asserts:
///   1. the response contains non-empty text, and
///   2. at least one emitted `Event` has a populated `interaction_id`.
///
/// Validates: Requirements 1.3, 4.1, 4.3.
#[tokio::test]
#[ignore = "requires GOOGLE_API_KEY/GEMINI_API_KEY, network access, and Interactions API (Beta) access"]
async fn single_turn_model_interaction_populates_interaction_id() {
    dotenvy::dotenv().ok();

    let Some(api_key) = api_key() else {
        eprintln!("skipping: GOOGLE_API_KEY/GEMINI_API_KEY not set");
        return;
    };

    // Transport toggle on the model — the agent/runner/tool loop are unchanged.
    let model = Arc::new(
        GeminiModel::new(api_key, "gemini-2.5-flash")
            .expect("failed to construct GeminiModel")
            .use_interactions_api(true)
            .expect("gemini-2.5-flash must be on the Interactions allowlist"),
    );

    let agent = Arc::new(
        LlmAgentBuilder::new("interactions-agent")
            .model(model)
            .instruction("You are a helpful assistant. Answer concisely.")
            .build()
            .expect("failed to build LlmAgent"),
    );

    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP_NAME.into(),
            user_id: USER_ID.into(),
            session_id: Some(SESSION_ID.into()),
            state: std::collections::HashMap::new(),
        })
        .await
        .expect("failed to create session");

    let runner = Runner::builder()
        .app_name(APP_NAME)
        .agent(agent as Arc<dyn Agent>)
        .session_service(sessions)
        .build()
        .expect("failed to build Runner");

    let mut stream = runner
        .run(
            UserId::new(USER_ID).expect("valid user id"),
            SessionId::new(SESSION_ID).expect("valid session id"),
            Content::new("user").with_text("Say hello in one sentence."),
        )
        .await
        .expect("runner.run failed");

    let mut full_text = String::new();
    let mut saw_interaction_id = false;

    while let Some(event) = stream.next().await {
        let event = event.expect("event stream yielded an error");

        // First-class interaction_id surfaced on the Event (ADK-Python parity
        // with `event.interaction_id`).
        if event.interaction_id().is_some() {
            saw_interaction_id = true;
        }

        if let Some(content) = &event.llm_response.content {
            for part in &content.parts {
                if let Part::Text { text } = part {
                    full_text.push_str(text);
                }
            }
        }
    }

    assert!(
        !full_text.trim().is_empty(),
        "expected the Interactions transport to produce text output",
    );
    assert!(
        saw_interaction_id,
        "expected at least one emitted Event to carry a populated interaction_id",
    );
}

/// Drives a single conversational turn through the runner on the given session,
/// collecting the concatenated model text and the first populated
/// `interaction_id` observed across the emitted events.
///
/// Returns `(text, interaction_id)` where `interaction_id` is `Some` when any
/// emitted `Event` carried a populated id (ADK-Python parity with
/// `event.interaction_id`).
async fn run_turn(runner: &Runner, session_id: &str, prompt: &str) -> (String, Option<String>) {
    let mut stream = runner
        .run(
            UserId::new(USER_ID).expect("valid user id"),
            SessionId::new(session_id).expect("valid session id"),
            Content::new("user").with_text(prompt),
        )
        .await
        .expect("runner.run failed");

    let mut full_text = String::new();
    let mut interaction_id: Option<String> = None;

    while let Some(event) = stream.next().await {
        let event = event.expect("event stream yielded an error");

        // Capture the first populated interaction_id for this turn.
        if interaction_id.is_none()
            && let Some(id) = event.interaction_id()
        {
            interaction_id = Some(id.to_string());
        }

        if let Some(content) = &event.llm_response.content {
            for part in &content.parts {
                if let Part::Text { text } = part {
                    full_text.push_str(text);
                }
            }
        }
    }

    (full_text, interaction_id)
}

/// ADK-Python parity Test 3: multi-turn stateful conversation via the runner.
///
/// Builds a `GeminiModel` with the Interactions transport enabled, wraps it in
/// a normal `LlmAgent`, and drives **two** sequential turns on the **same**
/// persistent session (the `InMemorySessionService` persists events between
/// `runner.run(...)` calls, so the agent populates
/// `LlmRequest.previous_response_id` from turn 1's `interaction_id` on turn 2).
///
/// Turn 1 establishes a fact ("My favorite color is blue. Remember it.") and
/// turn 2 asks the model to recall it ("What is my favorite color?"). The test
/// asserts:
///   1. both turns produced a populated `interaction_id` (`id1`, `id2` are
///      `Some`),
///   2. `id2 != id1` (each turn is a distinct, chained interaction), and
///   3. the turn-2 answer references "blue" (case-insensitive) — proving the
///      context was retained server-side via stateful continuation.
///
/// Note on "turn N+1 omits the transcript": that the chained turn sends only
/// the latest turn's content (and not the full transcript) is enforced and
/// covered by the conversion layer's unit/property tests in
/// `interactions_convert` (Property 4, stateful minimization). It is not
/// observable from the client side through the public `Runner`/`Event` API, so
/// here we assert the observable behavior instead: chained, distinct
/// `interaction_id`s plus retained conversational context.
///
/// Validates: Requirements 4, 5.
#[tokio::test]
#[ignore = "requires GOOGLE_API_KEY/GEMINI_API_KEY, network access, and Interactions API (Beta) access"]
async fn multi_turn_stateful_conversation_retains_context() {
    dotenvy::dotenv().ok();

    let Some(api_key) = api_key() else {
        eprintln!("skipping: GOOGLE_API_KEY/GEMINI_API_KEY not set");
        return;
    };

    // Transport toggle on the model — the agent/runner/tool loop are unchanged.
    let model = Arc::new(
        GeminiModel::new(api_key, "gemini-2.5-flash")
            .expect("failed to construct GeminiModel")
            .use_interactions_api(true)
            .expect("gemini-2.5-flash must be on the Interactions allowlist"),
    );

    let agent = Arc::new(
        LlmAgentBuilder::new("interactions-agent")
            .model(model)
            .instruction("You are a helpful assistant. Answer concisely.")
            .build()
            .expect("failed to build LlmAgent"),
    );

    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP_NAME.into(),
            user_id: USER_ID.into(),
            session_id: Some(MULTI_TURN_SESSION_ID.into()),
            state: std::collections::HashMap::new(),
        })
        .await
        .expect("failed to create session");

    let runner = Runner::builder()
        .app_name(APP_NAME)
        .agent(agent as Arc<dyn Agent>)
        .session_service(sessions)
        .build()
        .expect("failed to build Runner");

    // Turn 1: establish the fact. Capture the first interaction id (`id1`).
    let (_turn1_text, id1) =
        run_turn(&runner, MULTI_TURN_SESSION_ID, "My favorite color is blue. Remember it.").await;

    // Turn 2 (same session): ask the model to recall the fact. Because the
    // session persists turn 1's Event (carrying `interaction_id`), the agent
    // populates `previous_response_id` and the transport chains statefully via
    // `previous_interaction_id`.
    let (turn2_text, id2) =
        run_turn(&runner, MULTI_TURN_SESSION_ID, "What is my favorite color?").await;

    // Both turns produced a populated interaction_id.
    assert!(id1.is_some(), "expected turn 1 to carry a populated interaction_id",);
    assert!(id2.is_some(), "expected turn 2 to carry a populated interaction_id",);

    // Each turn is a distinct, chained interaction.
    assert_ne!(id1, id2, "expected turn 2 to be a distinct interaction chained from turn 1",);

    // Context was retained server-side: the turn-2 answer references "blue".
    assert!(
        turn2_text.to_lowercase().contains("blue"),
        "expected the turn-2 answer to recall the favorite color \"blue\", got: {turn2_text:?}",
    );
}
