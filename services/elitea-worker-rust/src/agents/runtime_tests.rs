use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::futures::stream;
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext, SessionId, UserId};
use async_trait::async_trait;
use tokio::sync::{Notify, Semaphore};

use super::runtime::{NativeAgentInvocation, NativeAgentRuntimeErrorCode};

struct GatedAgent {
    started: Arc<Notify>,
    release: Arc<Semaphore>,
}

#[async_trait]
impl Agent for GatedAgent {
    fn name(&self) -> &'static str {
        "invocation-scoped-agent"
    }

    fn description(&self) -> &'static str {
        "ADK cancellation isolation fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let started = Arc::clone(&self.started);
        let release = Arc::clone(&self.release);
        let invocation_id = context.invocation_id().to_owned();
        Ok(Box::pin(stream::unfold(
            Some((started, release, invocation_id)),
            |state| async move {
                let (started, release, invocation_id) = state?;
                started.notify_one();
                release.acquire().await.ok()?.forget();
                Some((Ok(Event::new(invocation_id)), None))
            },
        )))
    }
}

fn invocation(
    sessions: Arc<dyn SessionService>,
    started: Arc<Notify>,
    release: Arc<Semaphore>,
    input: &str,
) -> NativeAgentInvocation {
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(GatedAgent { started, release }))
        .session_service(sessions)
        .build()
        .expect("invocation-scoped fixture runner");
    NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new("shared-session").expect("fixture session ID"),
        Content::new("user").with_text(input),
    )
}

#[tokio::test]
async fn stop_is_scoped_to_one_same_session_native_invocation() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("shared-session".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("shared fixture session");
    let first_started = Arc::new(Notify::new());
    let second_started = Arc::new(Notify::new());
    let first_release = Arc::new(Semaphore::new(0));
    let second_release = Arc::new(Semaphore::new(0));
    let first_fanout = Arc::new(crate::agents::graph::fanout_control::FanoutCancellation::new());
    let second_fanout = Arc::new(crate::agents::graph::fanout_control::FanoutCancellation::new());
    let mut first = invocation(
        Arc::clone(&sessions),
        Arc::clone(&first_started),
        first_release,
        "first",
    )
    .with_fanout_stop(Arc::clone(&first_fanout))
    .start()
    .expect("first native invocation");
    let mut second = invocation(
        sessions,
        Arc::clone(&second_started),
        Arc::clone(&second_release),
        "second",
    )
    .with_fanout_stop(Arc::clone(&second_fanout))
    .start()
    .expect("second native invocation");

    let first_entered = first_started.notified();
    tokio::pin!(first_entered);
    tokio::select! {
        biased;
        event = first.next_event() => panic!("first event completed before release: {event:?}"),
        () = &mut first_entered => {}
    }
    let second_entered = second_started.notified();
    tokio::pin!(second_entered);
    tokio::select! {
        biased;
        event = second.next_event() => panic!("second event completed before release: {event:?}"),
        () = &mut second_entered => {}
    }

    assert!(first.request_stop());
    // A durable Stop also stops this run's fan-out, and only this run's (FU-064).
    assert!(first_fanout.is_cancelled());
    assert!(!second_fanout.is_cancelled());
    assert!(
        tokio::time::timeout(Duration::from_secs(1), first.next_event())
            .await
            .expect("first Stop is bounded")
            .expect("first Stop is not an ADK error")
            .is_none()
    );
    assert!(
        tokio::time::timeout(Duration::from_millis(10), second.next_event())
            .await
            .is_err(),
        "stopping one invocation must not stop a same-session sibling"
    );

    second_release.add_permits(1);
    assert!(
        tokio::time::timeout(Duration::from_secs(1), second.next_event())
            .await
            .expect("second event is bounded")
            .expect("second event succeeds")
            .is_some()
    );
    assert!(
        second
            .next_event()
            .await
            .expect("second end-of-stream")
            .is_none()
    );
}

#[tokio::test]
async fn completed_native_stream_rejects_reuse_with_safe_operator_error() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("shared-session".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("shared fixture session");
    let release = Arc::new(Semaphore::new(1));
    let mut run = invocation(sessions, Arc::new(Notify::new()), release, "complete")
        .start()
        .expect("native invocation");

    assert!(run.next_event().await.expect("fixture event").is_some());
    assert!(run.next_event().await.expect("fixture end").is_none());
    let error = run.next_event().await.expect_err("completed stream reuse");
    assert_eq!(error.code(), NativeAgentRuntimeErrorCode::InvalidState);
    assert_eq!(error.code().as_str(), "native_agent.invalid_state");
    assert_eq!(
        error.to_string(),
        "the native agent runner is not available for one exclusive invocation"
    );
    assert_eq!(
        format!("{error:?}"),
        "NativeAgentRuntimeError { code: InvalidState, .. }"
    );
}
