//! Keep durable control records separate from the Runner's model history view.

use adk_rust::Event;
use adk_rust::session::{Events, Session, State};
use chrono::{DateTime, Utc};

pub(super) fn project(inner: Box<dyn Session>) -> Box<dyn Session> {
    // The admitted session bounds this snapshot. Keep the original session for
    // state and identity; never write the projected events back to storage.
    let mut events = inner.events().all();
    for event in &mut events {
        if event.actions.tool_confirmation.is_some() {
            event.llm_response.content = None;
        }
    }
    Box::new(RunnerHistorySession { inner, events })
}

struct RunnerHistorySession {
    inner: Box<dyn Session>,
    events: Vec<Event>,
}

impl Session for RunnerHistorySession {
    fn id(&self) -> &str {
        self.inner.id()
    }
    fn app_name(&self) -> &str {
        self.inner.app_name()
    }
    fn user_id(&self) -> &str {
        self.inner.user_id()
    }
    fn state(&self) -> &dyn State {
        self.inner.state()
    }
    fn events(&self) -> &dyn Events {
        self
    }
    fn last_update_time(&self) -> DateTime<Utc> {
        self.inner.last_update_time()
    }
}

impl Events for RunnerHistorySession {
    fn all(&self) -> Vec<Event> {
        self.events.clone()
    }
    fn len(&self) -> usize {
        self.events.len()
    }
    fn at(&self, index: usize) -> Option<&Event> {
        self.events.get(index)
    }
}
