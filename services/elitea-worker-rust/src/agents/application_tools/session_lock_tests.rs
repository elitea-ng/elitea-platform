//! The application-tool wrapper has the same shape as the pipeline node-event
//! wrapper: a forwarded child event must never strand the root agent's child
//! model-scope append while it holds the root session writer.

use std::sync::Arc;

use tokio::sync::{Mutex, mpsc};

use super::{
    APPLICATION_EVENT_CHANNEL_CAPACITY, ApplicationEventReceiver, ApplicationEventSignal,
    ApplicationEventStreamingAgent,
};
use crate::agents::events::ApplicationToolPresentationCatalog;
use crate::agents::graph::session_lock_tests::{
    ChildAppendThenAnswer, LockFixture, assert_no_self_deadlock, node_event,
};

#[tokio::test]
async fn child_event_yield_never_strands_a_child_append_holding_the_root_writer() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping application-event session lock test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let fixture = LockFixture::create(&url).await;
    let (sender, receiver) = mpsc::channel(APPLICATION_EVENT_CHANNEL_CAPACITY);
    let agent = ApplicationEventStreamingAgent::new(
        Arc::new(ChildAppendThenAnswer {
            child: fixture.child.clone(),
        }),
        ApplicationEventReceiver {
            inner: Arc::new(Mutex::new(Some(receiver))),
        },
        ApplicationToolPresentationCatalog::default(),
    );
    let runner = fixture.runner(Arc::new(agent));
    assert_no_self_deadlock(&fixture, runner, || async move {
        sender
            .send(ApplicationEventSignal::ContainerEvent(Box::new(
                node_event(),
            )))
            .await
            .map_err(|_| ())
            .expect("queue child event");
    })
    .await;
    fixture.side.close().await;
}
