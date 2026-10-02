//! Keep durable control records separate from the Runner's model history view.

use std::collections::HashMap;

use adk_rust::Event;
use adk_rust::session::{Events, Session, State};
use chrono::{DateTime, Utc};
use serde_json::Value;

pub(super) fn project(
    inner: Box<dyn Session>,
    compaction_scope: Option<([u8; 32], &str)>,
) -> adk_rust::Result<Box<dyn Session>> {
    // The admitted session bounds this snapshot. Copy identity and state, then
    // release the original event payloads. Never persist the projected events.
    let mut events = inner.events().all();
    for event in &mut events {
        if event.actions.tool_confirmation.is_some() {
            event.llm_response.content = None;
        }
    }
    if let Some((definition, agent_name)) = compaction_scope {
        project_snapshot(&mut events, definition, agent_name)?;
        project_compaction(&mut events, inner.state(), definition, agent_name)?;
    }
    let projected = RunnerHistorySession {
        app_name: inner.app_name().to_owned(),
        user_id: inner.user_id().to_owned(),
        session_id: inner.id().to_owned(),
        state: inner.state().all(),
        updated_at: inner.last_update_time(),
        events,
    };
    drop(inner);
    Ok(Box::new(projected))
}

fn project_snapshot(
    events: &mut Vec<Event>,
    definition: [u8; 32],
    agent_name: &str,
) -> adk_rust::Result<()> {
    if events.iter().any(|event| !event.branch.is_empty()) {
        return Ok(());
    }
    let Some(marker) = events.iter().rfind(|event| {
        event.author == "elitea-recovery"
            && event
                .actions
                .state_delta
                .contains_key(super::model_checkpoint::HISTORY_SNAPSHOT_KEY)
    }) else {
        return Ok(());
    };
    let contents = super::model_checkpoint::history_contents(marker, definition, agent_name)?;
    let prefix = format!("model-history-{}-", marker.id);
    let marker_id = marker.id.clone();
    let invocation_id = marker.invocation_id.clone();
    let timestamp = marker.timestamp;
    // Re-projecting a view must not duplicate synthetic history events.
    events.retain(|event| !event.id.starts_with(&prefix));
    let Some(index) = events.iter().position(|event| event.id == marker_id) else {
        return Ok(());
    };
    for event in &mut events[..index] {
        if event.author == "user" || event.author == agent_name {
            event.llm_response.content = None;
        }
    }
    let history = contents.into_iter().enumerate().map(|(ordinal, content)| {
        let mut event = Event::with_id(format!("{prefix}{ordinal}"), &invocation_id);
        if content.role == "user" {
            "user"
        } else {
            agent_name
        }
        .clone_into(&mut event.author);
        event.timestamp = timestamp;
        event.set_content(content);
        event
    });
    events.splice(index..index, history);
    Ok(())
}

fn project_compaction(
    events: &mut Vec<Event>,
    state: &dyn State,
    definition: [u8; 32],
    agent_name: &str,
) -> adk_rust::Result<()> {
    let Some(record) = state
        .get(super::context_compaction::STATE_KEY)
        .filter(|v| !v.is_null())
    else {
        return Ok(());
    };
    // Shared branch history needs its own coverage mapping. Never apply a root
    // prefix across a sibling or an existing native timestamp compaction.
    if events
        .iter()
        .any(|event| !event.branch.is_empty() || event.actions.compaction.is_some())
    {
        return Ok(());
    }
    let source = events
        .iter()
        .enumerate()
        .filter_map(|(index, event)| {
            if event.author != "user" && event.author != agent_name {
                return None;
            }
            let mut content = event.llm_response.content.clone()?;
            content.role = match (event.author.as_str(), content.role.as_str()) {
                ("user", _) => "user",
                (_, "function" | "tool") => content.role.as_str(),
                _ => "model",
            }
            .to_owned();
            Some((index, content))
        })
        .collect::<Vec<_>>();
    let contents = source
        .iter()
        .map(|(_, content)| content.clone())
        .collect::<Vec<_>>();
    let Some((covered, replacement)) =
        super::context_compaction::history_replacement(record, definition, &contents)?
    else {
        return Ok(());
    };
    let last_index = source[covered].0;
    let marker = &events[last_index];
    let projected = replacement
        .into_iter()
        .enumerate()
        .map(|(index, content)| {
            let mut event = Event::with_id(
                format!("context-view-{}-{index}", marker.id),
                &marker.invocation_id,
            );
            "user".clone_into(&mut event.author);
            event.timestamp = marker.timestamp;
            event.set_content(content);
            event
        })
        .collect::<Vec<_>>();
    for (index, _) in source.iter().skip(1).take(covered) {
        events[*index].llm_response.content = None;
    }
    let insertion = last_index + 1;
    events.splice(insertion..insertion, projected);
    Ok(())
}

struct RunnerHistorySession {
    app_name: String,
    user_id: String,
    session_id: String,
    state: HashMap<String, Value>,
    updated_at: DateTime<Utc>,
    events: Vec<Event>,
}

impl Session for RunnerHistorySession {
    fn id(&self) -> &str {
        &self.session_id
    }
    fn app_name(&self) -> &str {
        &self.app_name
    }
    fn user_id(&self) -> &str {
        &self.user_id
    }
    fn state(&self) -> &dyn State {
        self
    }
    fn events(&self) -> &dyn Events {
        self
    }
    fn last_update_time(&self) -> DateTime<Utc> {
        self.updated_at
    }
}

impl State for RunnerHistorySession {
    fn get(&self, key: &str) -> Option<Value> {
        self.state.get(key).cloned()
    }

    fn set(&mut self, key: String, value: Value) {
        if adk_rust::validate_state_key(&key).is_ok() {
            self.state.insert(key, value);
        }
    }

    fn all(&self) -> HashMap<String, Value> {
        self.state.clone()
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

#[cfg(test)]
mod tests {
    use super::*;
    use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
    use std::sync::Arc;

    #[test]
    fn snapshot_restores_history_once_and_keeps_later_events_and_controls() {
        use adk_rust::{Content, LlmRequest};
        use serde_json::json;

        let mut control = Event::with_id("approval", "invocation-1");
        control.author = "agent".into();
        control.actions.transfer_to_agent = Some("child".into());
        control.set_content(Content::new("model").with_text("old content"));
        let contents = vec![
            Content::new("system").with_text("old system instructions"),
            Content::new("user").with_text("Original instruction"),
            Content::new("model").with_text("Saved result"),
            Content::new("tool").with_text("Saved tool result"),
        ];
        let request = LlmRequest::new("model", contents);
        let mut marker = Event::with_id("snapshot", "invocation-1");
        marker.author = "elitea-recovery".into();
        marker.actions.state_delta.insert(
            super::super::model_checkpoint::HISTORY_SNAPSHOT_KEY.into(),
            json!({"version":1,"agent_name":"agent"}),
        );
        marker.actions.state_delta.insert(
            super::super::model_checkpoint::CHECKPOINT_KEY.into(),
            json!({"version":1,"execution_id":"execution-1","generation":1,
                "definition_digest":vec![34;32],"invocation_id":"invocation-1",
                "phase":"model_pending","model":{"request":request,"tools":{}}}),
        );
        let mut later = Event::with_id("later", "invocation-1");
        later.author = "user".into();
        later.set_content(Content::new("user").with_text("New steering"));
        let durable = vec![control, marker, later];
        let mut events = durable.clone();
        project_snapshot(&mut events, [34; 32], "agent").unwrap();
        let first = serde_json::to_value(&events).unwrap();
        project_snapshot(&mut events, [34; 32], "agent").unwrap();
        assert_eq!(serde_json::to_value(&events).unwrap(), first);
        assert_eq!(
            events[0].actions.transfer_to_agent.as_deref(),
            Some("child")
        );
        assert!(events[0].llm_response.content.is_none());
        assert!(durable[0].llm_response.content.is_some());
        let contents = events
            .iter()
            .filter_map(|event| event.llm_response.content.clone())
            .collect::<Vec<_>>();
        assert_eq!(
            serde_json::to_value(contents).unwrap(),
            serde_json::to_value(vec![
                Content::new("user").with_text("Original instruction"),
                Content::new("model").with_text("Saved result"),
                Content::new("tool").with_text("Saved tool result"),
                Content::new("user").with_text("New steering"),
            ])
            .unwrap()
        );
        assert!(project_snapshot(&mut durable.clone(), [35; 32], "agent").is_err());
        assert!(project_snapshot(&mut durable.clone(), [34; 32], "sibling").is_err());
    }

    struct WatchedSession {
        inner: Box<dyn Session>,
        _lifetime: Arc<()>,
    }

    impl Session for WatchedSession {
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
            self.inner.events()
        }
        fn last_update_time(&self) -> DateTime<Utc> {
            self.inner.last_update_time()
        }
    }

    #[tokio::test]
    async fn projection_releases_source_snapshot_and_keeps_identity_and_state() {
        let service = InMemorySessionService::new();
        let source = service
            .create(CreateRequest {
                app_name: "history-view".into(),
                user_id: "user-1".into(),
                session_id: Some("session-1".into()),
                state: [("confirmed_fact".to_owned(), serde_json::json!("teal"))].into(),
            })
            .await
            .unwrap();
        let timestamp = source.last_update_time();
        let lifetime = Arc::new(());
        let observed = Arc::downgrade(&lifetime);
        let projected = project(
            Box::new(WatchedSession {
                inner: source,
                _lifetime: lifetime,
            }),
            None,
        )
        .unwrap();
        assert!(
            observed.upgrade().is_none(),
            "old event snapshot must not remain owned by the view"
        );
        assert_eq!(projected.id(), "session-1");
        assert_eq!(projected.app_name(), "history-view");
        assert_eq!(projected.user_id(), "user-1");
        assert_eq!(projected.last_update_time(), timestamp);
        assert_eq!(
            projected.state().get("confirmed_fact"),
            Some(serde_json::json!("teal"))
        );
    }
}
