use std::os::unix::fs::PermissionsExt as _;

use adk_core::{Content, FunctionResponseData};
use serde_json::json;

use super::*;

fn identity(user: i64, deployment: &str) -> SessionIdentity {
    SessionIdentity {
        deployment: deployment.to_owned(),
        user_id: user,
        project_id: 1,
        conversation_uuid: "99999999-2222-4333-8444-555555555555".to_owned(),
    }
}

fn text_event(invocation: &str, author: &str, role: &str, text: &str) -> Event {
    let mut event = Event::new(invocation);
    event.author = author.to_owned();
    event.llm_response.content = Some(Content {
        role: role.to_owned(),
        parts: vec![Part::Text {
            text: text.to_owned(),
        }],
    });
    event
}

fn call_event(invocation: &str, id: &str) -> Event {
    let mut event = Event::new(invocation);
    event.author = "agent".into();
    event.llm_response.content = Some(Content {
        role: "model".into(),
        parts: vec![Part::FunctionCall {
            name: "read_file".into(),
            args: json!({"path": "a"}),
            id: Some(id.to_owned()),
            thought_signature: None,
        }],
    });
    event
}

fn response_event(invocation: &str, id: &str, body: Value) -> Event {
    let mut event = Event::new(invocation);
    event.author = "agent".into();
    event.llm_response.content = Some(Content {
        role: "function".into(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new("read_file", body),
            id: Some(id.to_owned()),
            annotations: None,
        }],
    });
    event
}

fn new(turn: &str, event: Event) -> NewEvent {
    NewEvent {
        turn_execution_id: turn.to_owned(),
        executed_by: EXECUTED_BY_DESKTOP,
        response_message_id: Some("answer".into()),
        event,
    }
}

fn open() -> (tempfile::TempDir, DesktopStateStore) {
    let dir = tempfile::tempdir().unwrap();
    let store = DesktopStateStore::open(dir.path()).unwrap();
    (dir, store)
}

#[test]
fn keys_isolate_users_and_deployments() {
    let base = identity(1, "https://a.example");
    assert_eq!(base.key(), identity(1, "https://a.example/").key());
    assert_ne!(base.key(), identity(2, "https://a.example").key());
    assert_ne!(base.key(), identity(1, "https://b.example").key());
    let mut other = base.clone();
    other.conversation_uuid = "99999999-2222-4333-8444-555555555556".into();
    assert_ne!(base.key(), other.key());
    // The workspace and project are not part of the key.
    let mut project = base.clone();
    project.project_id = 77;
    assert_eq!(base.key(), project.key());
    // Separated: user 1 + uuid "2…" never collides with user 12 + uuid "…".
    assert_ne!(session_key("d", 1, "2abc"), session_key("d", 12, "abc"),);
}

#[tokio::test]
async fn another_user_or_deployment_reads_nothing() {
    let (_dir, store) = open();
    let mine = identity(1, "https://a.example");
    let sessions = store.store();
    sessions.open_session(&mine, "fp").await.unwrap();
    sessions
        .seed(
            &mine.key(),
            vec![new("t", text_event("i", "user", "user", "secret plan"))],
            Some("2".into()),
            &[],
        )
        .await
        .unwrap();
    for other in [
        identity(2, "https://a.example"),
        identity(1, "https://b.example"),
    ] {
        let state = sessions.open_session(&other, "fp").await.unwrap();
        assert_eq!(state.events, 0, "{other:?}");
        assert!(!state.seeded_from_server);
        assert!(sessions.rows(&other.key()).await.unwrap().is_empty());
        // And through the adk service the other key is a different session.
        let service = store.sessions();
        let session = service
            .get(GetRequest {
                app_name: APP_NAME.into(),
                user_id: ADK_USER.into(),
                session_id: other.key(),
                num_recent_events: None,
                after: None,
            })
            .await
            .unwrap();
        assert_eq!(session.events().len(), 0);
    }
    assert_eq!(sessions.rows(&mine.key()).await.unwrap().len(), 1);
}

#[test]
fn refuses_loose_or_foreign_file() {
    // Loose: group/world readable.
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(FILE_NAME);
    fs::write(&path, b"").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
    let error = SessionStore::open(dir.path()).err().expect("refused");
    assert!(
        error.to_string().contains("readable or writable"),
        "{error}"
    );

    // A symlink, even to a fine file.
    let dir = tempfile::tempdir().unwrap();
    let target = dir.path().join("elsewhere.sqlite");
    fs::write(&target, b"").unwrap();
    fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
    std::os::unix::fs::symlink(&target, dir.path().join(FILE_NAME)).unwrap();
    let error = SessionStore::open(dir.path()).err().expect("refused");
    assert!(error.to_string().contains("symbolic link"), "{error}");

    // Not a regular file.
    let dir = tempfile::tempdir().unwrap();
    fs::create_dir(dir.path().join(FILE_NAME)).unwrap();
    assert!(SessionStore::open(dir.path()).is_err());

    // A loose sidecar.
    let dir = tempfile::tempdir().unwrap();
    let wal = dir.path().join(format!("{FILE_NAME}-wal"));
    fs::write(&wal, b"").unwrap();
    fs::set_permissions(&wal, fs::Permissions::from_mode(0o666)).unwrap();
    assert!(SessionStore::open(dir.path()).is_err());

    // A file that is not a database, and a newer schema.
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(FILE_NAME);
    fs::write(&path, b"this is not sqlite at all, not even close, no").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    assert!(SessionStore::open(dir.path()).is_err());
    let dir = tempfile::tempdir().unwrap();
    {
        let store = SessionStore::open(dir.path()).unwrap();
        store
            .call_blocking("t", |conn| conn.execute_batch("PRAGMA user_version = 99"))
            .unwrap();
    }
    let error = SessionStore::open(dir.path()).err().expect("newer");
    assert!(error.to_string().contains("newer version"), "{error}");
}

#[test]
fn the_store_is_owner_only_in_an_owner_only_directory() {
    let parent = tempfile::tempdir().unwrap();
    let dir = parent.path().join("data");
    let store = SessionStore::open(&dir).unwrap();
    let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&dir), 0o700);
    assert_eq!(mode(store.path()), 0o600);
    // A wider directory is narrowed.
    drop(store);
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
    let store = SessionStore::open(&dir).unwrap();
    assert_eq!(mode(&dir), 0o700);
    // Whatever SQLite adds next to it follows the database's mode.
    for name in [format!("{FILE_NAME}-wal"), format!("{FILE_NAME}-shm")] {
        let path = dir.join(name);
        if path.exists() {
            assert_eq!(mode(&path), 0o600);
        }
    }
    drop(store);
}

#[tokio::test]
async fn trims_whole_invocations() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    let big = "x".repeat(2_000);
    let mut events = Vec::new();
    for turn in 0..6 {
        let inv = format!("inv-{turn}");
        events.push(new(&inv, text_event(&inv, "user", "user", "question")));
        events.push(new(&inv, call_event(&inv, &format!("c{turn}"))));
        events.push(new(
            &inv,
            response_event(&inv, &format!("c{turn}"), json!({"output": big})),
        ));
        events.push(new(&inv, text_event(&inv, "agent", "model", "answer")));
    }
    sessions.seed(&key, events, None, &[]).await.unwrap();
    let total: i64 = sessions
        .call("t", {
            let key = key.clone();
            move |conn| {
                conn.query_row(
                    "SELECT SUM(bytes) FROM events WHERE session_key = ?1",
                    params![key],
                    |row| row.get(0),
                )
            }
        })
        .await
        .unwrap();
    assert!(total > 12_000);
    // Squeeze to about three invocations.
    let limit = total / 2 + 100;
    let trimmed = sessions
        .call("t", {
            let key = key.clone();
            move |conn| {
                let tx = conn.transaction()?;
                let trimmed = trim_to(&tx, &key, limit)?;
                tx.commit()?;
                Ok(trimmed)
            }
        })
        .await
        .unwrap();
    assert!(trimmed);
    let rows = sessions.rows(&key).await.unwrap();
    // Whole invocations only: each one has all four events, the OLDEST are
    // gone, the newest stays, and no response is left without its call.
    let mut by_inv: Vec<(String, usize)> = Vec::new();
    for row in &rows {
        match by_inv.last_mut() {
            Some((inv, count)) if *inv == row.invocation_id => *count += 1,
            _ => by_inv.push((row.invocation_id.clone(), 1)),
        }
    }
    assert!(by_inv.iter().all(|(_, count)| *count == 4), "{by_inv:?}");
    assert!(by_inv.len() < 6 && by_inv.len() >= 2, "{by_inv:?}");
    assert_eq!(by_inv.last().unwrap().0, "inv-5");
    assert_eq!(
        by_inv.first().unwrap().0,
        format!("inv-{}", 6 - by_inv.len())
    );
    let calls: Vec<_> = rows
        .iter()
        .filter_map(|r| match &r.event.llm_response.content.as_ref()?.parts[0] {
            Part::FunctionCall { id, .. } => id.clone(),
            _ => None,
        })
        .collect();
    let responses: Vec<_> = rows
        .iter()
        .filter_map(|r| match &r.event.llm_response.content.as_ref()?.parts[0] {
            Part::FunctionResponse { id, .. } => id.clone(),
            _ => None,
        })
        .collect();
    assert_eq!(calls, responses, "no orphan FunctionResponse");
    // The newest invocation alone is never dropped, however small the limit.
    sessions
        .call("t", {
            let key = key.clone();
            move |conn| {
                let tx = conn.transaction()?;
                trim_to(&tx, &key, 1)?;
                tx.commit()
            }
        })
        .await
        .unwrap();
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.len(), 4);
    assert!(rows.iter().all(|r| r.invocation_id == "inv-5"));
}

fn state_event(invocation: &str, entries: &[(&str, Value)]) -> Event {
    let mut event = Event::new(invocation);
    event.author = "agent".into();
    for (name, value) in entries {
        event
            .actions
            .state_delta
            .insert((*name).to_owned(), value.clone());
    }
    event
}

async fn session_state(store: &DesktopStateStore, key: &str) -> HashMap<String, Value> {
    store
        .sessions()
        .get(GetRequest {
            app_name: APP_NAME.into(),
            user_id: ADK_USER.into(),
            session_id: key.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap()
        .state()
        .all()
}

async fn trim_session(sessions: &SessionStore, key: &str, limit: i64) -> bool {
    sessions
        .call("t", {
            let key = key.to_owned();
            move |conn| {
                let tx = conn.transaction()?;
                let trimmed = trim_to(&tx, &key, limit)?;
                tx.commit()?;
                Ok(trimmed)
            }
        })
        .await
        .unwrap()
}

#[tokio::test]
async fn a_trim_keeps_the_state_the_dropped_invocations_built() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    let big = "x".repeat(2_000);
    let choice = json!({"name": "gpt-4o", "reasoning_effort": "low"});
    let mut events = vec![
        // The thread's model choice, its own invocation (as the turn keeps
        // it), then a key set and cleared, then one overwritten later.
        new(
            "t0",
            state_event("choice", &[("elitea.model.v1", choice.clone())]),
        ),
        new(
            "t0",
            state_event("s1", &[("cleared", json!(1)), ("later", json!("old"))]),
        ),
        new("t0", state_event("s2", &[("cleared", Value::Null)])),
    ];
    for turn in 0..4 {
        let inv = format!("inv-{turn}");
        events.push(new(&inv, text_event(&inv, "user", "user", &big)));
    }
    events.push(new("t9", state_event("s9", &[("later", json!("new"))])));
    events.push(new("t9", text_event("inv-9", "user", "user", "newest")));
    sessions.seed(&key, events, None, &[]).await.unwrap();
    let before = session_state(&store, &key).await;
    assert_eq!(before.get("elitea.model.v1"), Some(&choice));

    assert!(trim_session(sessions, &key, 3_000).await);
    let rows = sessions.rows(&key).await.unwrap();
    // The text invocations went, and the state events with them; one event
    // at the head now carries what they had set.
    assert!(rows.iter().all(|r| r.invocation_id != "choice"), "{rows:?}");
    assert!(rows.iter().all(|r| r.invocation_id != "inv-0"), "{rows:?}");
    assert_eq!(rows[0].turn_execution_id, CARRIED_STATE_TURN);
    assert!(rows[0].event.llm_response.content.is_none());
    assert_eq!(
        rows[0].response_message_id.as_deref(),
        Some(CARRIED_STATE_TURN)
    );
    let after = session_state(&store, &key).await;
    assert_eq!(after.get("elitea.model.v1"), Some(&choice));
    assert_eq!(after.get("later"), Some(&json!("new")), "a kept event wins");
    assert_eq!(after.get("cleared"), None);
    // The same state: a key cleared before is simply absent now.
    let mut set = before.clone();
    set.retain(|_, value| !value.is_null());
    assert_eq!(after, set);

    // A later trim folds the carried event again, with what goes then.
    assert!(trim_session(sessions, &key, 1).await);
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.len(), 2, "{rows:?}");
    assert_eq!(rows[0].turn_execution_id, CARRIED_STATE_TURN);
    assert_eq!(rows[1].invocation_id, "inv-9");
    assert_eq!(session_state(&store, &key).await, set);
    // A carried event survives a session reopen (it has an answer id).
    sessions.open_session(&me, "fp").await.unwrap();
    assert_eq!(session_state(&store, &key).await, set);
}

#[tokio::test]
async fn a_reseed_keeps_only_the_state_its_caller_names() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    let choice = json!({"name": "gpt-4o"});
    sessions
        .seed(
            &key,
            vec![
                new(
                    "t",
                    state_event("c", &[("elitea.model.v1", choice.clone())]),
                ),
                new("t", state_event("p", &[("plan", json!({"steps": []}))])),
                new("t", text_event("i", "user", "user", "old")),
            ],
            Some("1".into()),
            &[],
        )
        .await
        .unwrap();
    sessions
        .seed(
            &key,
            vec![new("s", text_event("s", "user", "user", "from the server"))],
            Some("3".into()),
            &["elitea.model.v1"],
        )
        .await
        .unwrap();
    let state = session_state(&store, &key).await;
    assert_eq!(state.get("elitea.model.v1"), Some(&choice));
    assert_eq!(state.get("plan"), None, "only the named keys stay");
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0].turn_execution_id, CARRIED_STATE_TURN);
    // A choice cleared since (a state delta of `null`) is not carried.
    sessions
        .seed(
            &key,
            vec![new(
                "t",
                state_event("n", &[("elitea.model.v1", Value::Null)]),
            )],
            Some("4".into()),
            &["elitea.model.v1"],
        )
        .await
        .unwrap();
    assert_eq!(
        session_state(&store, &key).await.get("elitea.model.v1"),
        Some(&Value::Null)
    );
    sessions
        .seed(&key, Vec::new(), Some("4".into()), &["elitea.model.v1"])
        .await
        .unwrap();
    assert_eq!(sessions.rows(&key).await.unwrap().len(), 0);
}

#[tokio::test]
async fn a_session_past_its_size_is_trimmed_and_asks_to_be_seeded_again() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(&key, Vec::new(), Some("0".into()), &[])
        .await
        .unwrap();
    // Appends past 32 MiB: 40 turns of 1 MiB user text (user text is not cut
    // at the 64 KiB tool-response cap, so the fill survives whole).
    let text = "y".repeat(1024 * 1024);
    let mut events = Vec::new();
    for turn in 0..40 {
        let inv = format!("inv-{turn}");
        events.push(new(&inv, text_event(&inv, "user", "user", &text)));
    }
    sessions
        .call("t", {
            let key = key.clone();
            move |conn| {
                let tx = conn.transaction()?;
                for (i, event) in events.into_iter().enumerate() {
                    insert_event(&tx, &key, i64::try_from(i + 1).unwrap(), event)?;
                }
                tx.commit()
            }
        })
        .await
        .unwrap();
    let guard = sessions.begin_turn(&key, "t-new").unwrap();
    let service = store.sessions();
    service
        .append_event(&key, text_event("inv-new", "user", "user", "go"))
        .await
        .unwrap();
    sessions
        .finish_turn(
            &key,
            "t-new",
            TurnOutcome::Committed {
                response_message_id: "r".into(),
                cursor: Some("42".into()),
            },
        )
        .await
        .unwrap();
    drop(guard);
    let info = sessions
        .info(&me.deployment, me.user_id, &me.conversation_uuid)
        .await
        .unwrap()
        .unwrap();
    assert!(
        i64::try_from(info.bytes).unwrap() <= MAX_SESSION_BYTES,
        "{info:?}"
    );
    assert!(!info.seeded_from_server, "the head is re-seeded next time");
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.last().unwrap().invocation_id, "inv-new");
}

#[tokio::test]
async fn a_turn_appends_tagged_events_and_they_survive_a_restart() {
    let dir = tempfile::tempdir().unwrap();
    let me = identity(1, "https://a.example");
    let key = me.key();
    {
        let state = DesktopStateStore::open(dir.path()).unwrap();
        let sessions = state.store();
        let opened = sessions.open_session(&me, "fp").await.unwrap();
        assert!(!opened.seeded_from_server);
        sessions
            .seed(&key, Vec::new(), Some("0".into()), &[])
            .await
            .unwrap();
        // Nothing but a running turn writes.
        let service = state.sessions();
        assert!(
            service
                .append_event(&key, text_event("i", "user", "user", "hi"))
                .await
                .is_err()
        );
        let scope = sessions.begin_turn(&key, "exec-1").unwrap();
        assert!(
            sessions.begin_turn(&key, "exec-2").is_err(),
            "one turn per session"
        );
        service
            .append_event(&key, text_event("i", "user", "user", "hi"))
            .await
            .unwrap();
        service
            .append_event(&key, text_event("i", "agent", "model", "hello"))
            .await
            .unwrap();
        sessions
            .finish_turn(
                &key,
                "exec-1",
                TurnOutcome::Committed {
                    response_message_id: "resp-1".into(),
                    cursor: Some("2".into()),
                },
            )
            .await
            .unwrap();
        drop(scope);
        assert!(sessions.begin_turn(&key, "exec-3").is_ok(), "released");
    }
    // Reopened over the same directory.
    let state = DesktopStateStore::open(dir.path()).unwrap();
    let opened = state.store().open_session(&me, "fp").await.unwrap();
    assert!(opened.seeded_from_server);
    assert_eq!(opened.server_cursor.as_deref(), Some("2"));
    assert_eq!(opened.events, 2);
    let rows = state.store().rows(&key).await.unwrap();
    assert_eq!(rows[0].turn_execution_id, "exec-1");
    assert_eq!(rows[0].executed_by, EXECUTED_BY_DESKTOP);
    assert_eq!(rows[1].response_message_id.as_deref(), Some("resp-1"));
    let session = state
        .sessions()
        .get(GetRequest {
            app_name: APP_NAME.into(),
            user_id: ADK_USER.into(),
            session_id: key.clone(),
            num_recent_events: Some(1),
            after: None,
        })
        .await
        .unwrap();
    assert_eq!(session.events().len(), 1);
    let info = state
        .store()
        .info(&me.deployment, me.user_id, &me.conversation_uuid)
        .await
        .unwrap()
        .unwrap();
    assert_eq!((info.turns, info.seeded_from_server), (1, true));
}

#[tokio::test]
async fn a_cancelled_turn_leaves_nothing_and_a_refused_one_asks_for_a_seed() {
    let (_dir, state) = open();
    let sessions = state.store();
    let service = state.sessions();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(&key, Vec::new(), Some("0".into()), &[])
        .await
        .unwrap();

    let scope = sessions.begin_turn(&key, "kept").unwrap();
    service
        .append_event(&key, text_event("i1", "user", "user", "one"))
        .await
        .unwrap();
    sessions
        .finish_turn(
            &key,
            "kept",
            TurnOutcome::Committed {
                response_message_id: "r1".into(),
                cursor: Some("2".into()),
            },
        )
        .await
        .unwrap();
    drop(scope);

    let scope = sessions.begin_turn(&key, "cancelled").unwrap();
    service
        .append_event(&key, text_event("i2", "user", "user", "two"))
        .await
        .unwrap();
    service
        .append_event(&key, call_event("i2", "c"))
        .await
        .unwrap();
    sessions
        .finish_turn(&key, "cancelled", TurnOutcome::Cancelled)
        .await
        .unwrap();
    drop(scope);
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.len(), 1);
    assert_eq!(rows[0].turn_execution_id, "kept");

    let scope = sessions.begin_turn(&key, "refused").unwrap();
    service
        .append_event(&key, text_event("i3", "user", "user", "three"))
        .await
        .unwrap();
    sessions
        .finish_turn(&key, "refused", TurnOutcome::Refused)
        .await
        .unwrap();
    drop(scope);
    let reopened = sessions.open_session(&me, "fp").await.unwrap();
    assert!(
        !reopened.seeded_from_server,
        "a refused commit forces a re-seed"
    );
    // The refused turn never reached the server: opening dropped it.
    assert_eq!(reopened.events, 1);
}

#[tokio::test]
async fn a_turn_that_never_finished_is_dropped_when_the_session_opens_again() {
    let (_dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(
            &key,
            vec![new("seeded", text_event("s", "user", "user", "old"))],
            Some("2".into()),
            &[],
        )
        .await
        .unwrap();
    let scope = sessions.begin_turn(&key, "crashed").unwrap();
    state
        .sessions()
        .append_event(&key, text_event("c", "user", "user", "half"))
        .await
        .unwrap();
    // A second opening while the turn runs must not touch its events.
    let during = sessions.open_session(&me, "fp").await.unwrap();
    assert_eq!(during.events, 2);
    drop(scope); // the process "died" without settling
    let after = sessions.open_session(&me, "fp").await.unwrap();
    assert_eq!(after.events, 1);
}

#[tokio::test]
async fn a_tool_response_is_cut_and_binary_content_is_not_kept() {
    let (_dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    let huge = "z".repeat(MAX_TOOL_RESPONSE_BYTES * 2);
    let mut image = text_event("i", "user", "user", "look");
    image
        .llm_response
        .content
        .as_mut()
        .unwrap()
        .parts
        .push(Part::InlineData {
            mime_type: "image/png".into(),
            data: vec![1, 2, 3],
            uri: None,
            annotations: None,
        });
    sessions
        .seed(
            &key,
            vec![
                new("t", call_event("i", "c")),
                new("t", response_event("i", "c", json!({"output": huge}))),
                new("t", image),
            ],
            None,
            &[],
        )
        .await
        .unwrap();
    let rows = sessions.rows(&key).await.unwrap();
    let Part::FunctionResponse {
        function_response, ..
    } = &rows[1].event.llm_response.content.as_ref().unwrap().parts[0]
    else {
        panic!("a response")
    };
    let Value::String(text) = &function_response.response else {
        panic!("a cut response is text")
    };
    assert!(text.len() < MAX_TOOL_RESPONSE_BYTES + 100);
    assert!(
        text.contains("…[truncated "),
        "{}",
        &text[text.len() - 60..]
    );
    assert!(text.ends_with("bytes; re-run or re-read]"));
    let parts = &rows[2].event.llm_response.content.as_ref().unwrap().parts;
    assert!(matches!(&parts[1], Part::Text { text } if text == BINARY_NOTE));
}

#[tokio::test]
async fn a_tool_image_is_not_kept_but_its_metadata_is() {
    let (_dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    let image = json!({
        "kind": "image", "path": "a.png", "mime": "image/png", "bytes": 3,
        "image_base64": "z".repeat(MAX_TOOL_RESPONSE_BYTES * 2),
    });
    sessions
        .seed(
            &key,
            vec![
                new("t", call_event("i", "c")),
                new("t", response_event("i", "c", image)),
            ],
            None,
            &[],
        )
        .await
        .unwrap();
    let rows = sessions.rows(&key).await.unwrap();
    let Part::FunctionResponse {
        function_response, ..
    } = &rows[1].event.llm_response.content.as_ref().unwrap().parts[0]
    else {
        panic!("a response")
    };
    assert_eq!(
        function_response.response,
        json!({"kind": "image", "path": "a.png", "mime": "image/png", "bytes": 3})
    );
}

#[tokio::test]
async fn a_path_deny_change_drops_the_tool_response_bodies_and_asks_for_a_seed() {
    let (_dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    let loose = policy_fingerprint(&[]);
    let tight = policy_fingerprint(&["secrets/**".to_owned()]);
    assert_ne!(loose, tight);
    assert_eq!(
        policy_fingerprint(&["b".into(), "a".into(), "a".into()]),
        policy_fingerprint(&["a".into(), "b".into()])
    );
    sessions.open_session(&me, &loose).await.unwrap();
    sessions
        .seed(
            &key,
            vec![
                new("t", text_event("i", "user", "user", "read the key")),
                new("t", call_event("i", "c")),
                new(
                    "t",
                    response_event("i", "c", json!({"output": "TOP SECRET"})),
                ),
                new("t", text_event("i", "agent", "model", "I read it")),
            ],
            Some("2".into()),
            &[],
        )
        .await
        .unwrap();
    // The same policy again: untouched.
    let same = sessions.open_session(&me, &loose).await.unwrap();
    assert!(same.seeded_from_server && !same.bodies_dropped);
    // A different one: bodies gone, messages and calls stay, re-seed due.
    let changed = sessions.open_session(&me, &tight).await.unwrap();
    assert!(changed.bodies_dropped);
    assert!(!changed.seeded_from_server);
    assert_eq!(changed.server_cursor, None);
    assert_eq!(changed.events, 4);
    let rows = sessions.rows(&key).await.unwrap();
    let json =
        serde_json::to_string(&rows.iter().map(|r| r.event.clone()).collect::<Vec<_>>()).unwrap();
    assert!(!json.contains("TOP SECRET"), "{json}");
    assert!(json.contains(WITHHELD_BODY));
    assert!(json.contains("read the key") && json.contains("I read it"));
    // The fingerprint is now the new one.
    let again = sessions.open_session(&me, &tight).await.unwrap();
    assert!(!again.bodies_dropped);
}

#[tokio::test]
async fn lifecycle_marks_and_deletes() {
    let (_dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(
            &key,
            vec![new("t", text_event("i", "user", "user", "x"))],
            Some("2".into()),
            &[],
        )
        .await
        .unwrap();
    assert!(
        sessions
            .mark_reseed(&me.deployment, me.user_id, &me.conversation_uuid)
            .await
            .unwrap()
    );
    let state_after = sessions.open_session(&me, "fp").await.unwrap();
    assert!(!state_after.seeded_from_server);
    assert!(
        !sessions
            .mark_reseed(&me.deployment, 99, &me.conversation_uuid)
            .await
            .unwrap(),
        "no such session is not an error"
    );
    assert!(
        sessions
            .delete_conversation(&me.deployment, me.user_id, &me.conversation_uuid)
            .await
            .unwrap()
    );
    assert!(sessions.rows(&key).await.unwrap().is_empty());
    assert!(
        sessions
            .info(&me.deployment, me.user_id, &me.conversation_uuid)
            .await
            .unwrap()
            .is_none()
    );
    sessions.open_session(&me, "fp").await.unwrap();
    assert_eq!(sessions.delete_all().unwrap(), 1);
}

#[tokio::test]
async fn move_aside_keeps_the_old_file_and_starts_fresh() {
    let (dir, state) = open();
    let sessions = state.store();
    let me = identity(1, "https://a.example");
    sessions.open_session(&me, "fp").await.unwrap();
    let aside = sessions.move_aside().unwrap();
    assert!(aside.join(FILE_NAME).is_file(), "moved, never deleted");
    assert!(dir.path().join(FILE_NAME).is_file());
    let fresh = sessions.open_session(&me, "fp").await.unwrap();
    assert_eq!(fresh.events, 0);
    assert!(!fresh.seeded_from_server);
}

#[tokio::test]
async fn reclaim_keeps_the_events_and_reopens() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(
            &key,
            vec![
                new("i", text_event("i", "user", "user", "hi")),
                new("i", text_event("i", "agent", "model", "hello")),
            ],
            Some("0".into()),
            &[],
        )
        .await
        .unwrap();
    let held = sessions.rows(&key).await.unwrap();
    assert_eq!(held.len(), 2, "the events were stored");
    // The Doctor's `sessions.vacuum`: close, VACUUM, reopen.
    let before = sessions.reclaim().unwrap();
    assert!(before > 0, "the file held bytes");
    let back = sessions.rows(&key).await.unwrap();
    assert_eq!(back.len(), 2, "the events survived the shrink");
}

#[tokio::test]
async fn new_files_are_vacuumed_incrementally() {
    let (dir, store) = open();
    store
        .store()
        .open_session(&identity(1, "https://a.example"), "fp")
        .await
        .unwrap();
    let conn = rusqlite::Connection::open(dir.path().join(FILE_NAME)).unwrap();
    let mode: i64 = conn
        .query_row("PRAGMA auto_vacuum", [], |row| row.get(0))
        .unwrap();
    assert_eq!(mode, 2, "2 = INCREMENTAL");
    let version: i64 = conn
        .query_row("PRAGMA user_version", [], |row| row.get(0))
        .unwrap();
    assert_eq!(version, SCHEMA_VERSION);
}

#[tokio::test]
async fn an_existing_file_switches_to_incremental_vacuum_and_keeps_its_schema() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(FILE_NAME);
    {
        let store = SessionStore::open(dir.path()).unwrap();
        store
            .open_session(&identity(1, "https://a.example"), "fp")
            .await
            .unwrap();
    }
    // A file from before incremental vacuum: mode NONE, same schema.
    {
        let conn = rusqlite::Connection::open(&path).unwrap();
        conn.execute_batch("PRAGMA auto_vacuum = NONE; VACUUM;")
            .unwrap();
    }
    let mode = || -> i64 {
        rusqlite::Connection::open(&path)
            .unwrap()
            .query_row("PRAGMA auto_vacuum", [], |row| row.get(0))
            .unwrap()
    };
    assert_eq!(mode(), 0, "the file starts without it");
    let store = SessionStore::open(dir.path()).unwrap();
    assert_eq!(mode(), 2, "the open switched it to INCREMENTAL");
    let version: i64 = rusqlite::Connection::open(&path)
        .unwrap()
        .query_row("PRAGMA user_version", [], |row| row.get(0))
        .unwrap();
    assert_eq!(version, SCHEMA_VERSION, "the schema version is untouched");
    drop(store);
}

#[tokio::test]
async fn an_idle_store_folds_its_wal_into_the_database() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(FILE_NAME);
    // A short idle interval: the checkpoint runs long before 30 seconds.
    let store = SessionStore::open_at(dir.path(), Duration::from_millis(200)).unwrap();
    let me = identity(1, "https://a.example");
    let key = me.key();
    store.open_session(&me, "fp").await.unwrap();
    store
        .seed(
            &key,
            vec![new("i", text_event("i", "user", "user", "hi"))],
            Some("0".into()),
            &[],
        )
        .await
        .unwrap();
    // The turn's bytes are in the WAL now. Let the store sit idle, and its
    // idle checkpoint folds them back into the main file.
    tokio::time::sleep(Duration::from_millis(600)).await;
    let wal = std::fs::symlink_metadata(path.with_file_name(format!("{FILE_NAME}-wal")))
        .map(|meta| meta.len())
        .unwrap_or(0);
    assert!(
        wal <= 32,
        "the idle checkpoint leaves no turn bytes in the wal (holds {wal})"
    );
    drop(store);
}

#[tokio::test]
async fn the_adk_service_creates_lists_and_deletes() {
    let (_dir, state) = open();
    let service = state.sessions();
    let created = service
        .create(CreateRequest {
            app_name: APP_NAME.into(),
            user_id: ADK_USER.into(),
            session_id: Some("abc".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    assert_eq!(created.id(), "abc");
    assert_eq!(
        service
            .list(ListRequest {
                app_name: APP_NAME.into(),
                user_id: ADK_USER.into(),
                limit: None,
                offset: None,
            })
            .await
            .unwrap()
            .len(),
        1
    );
    service
        .delete(DeleteRequest {
            app_name: APP_NAME.into(),
            user_id: ADK_USER.into(),
            session_id: "abc".into(),
        })
        .await
        .unwrap();
    assert!(
        service
            .get(GetRequest {
                app_name: APP_NAME.into(),
                user_id: ADK_USER.into(),
                session_id: "abc".into(),
                num_recent_events: None,
                after: None,
            })
            .await
            .is_err()
    );
}

#[tokio::test]
async fn a_temp_state_key_is_never_persisted() {
    let (_dir, state) = open();
    let me = identity(1, "https://a.example");
    let key = me.key();
    state.store().open_session(&me, "fp").await.unwrap();
    let scope = state.store().begin_turn(&key, "exec-1").unwrap();
    let service = state.sessions();
    let mut event = call_event("inv", "c1");
    event.actions.state_delta.insert(
        format!("{}temp-runner", adk_session::KEY_PREFIX_TEMP),
        json!(1),
    );
    event.actions.state_delta.insert("kept".into(), json!(2));
    service.append_event(&key, event).await.unwrap();
    drop(scope);
    let rows = state.store().rows(&key).await.unwrap();
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert!(
        !rows[0]
            .event
            .actions
            .state_delta
            .contains_key("temp:temp-runner"),
        "the temp key was stripped: {:?}",
        rows[0].event.actions.state_delta
    );
    assert_eq!(
        rows[0].event.actions.state_delta.get("kept"),
        Some(&json!(2))
    );
}

#[test]
fn an_adk_event_round_trips_through_plain_serde_json() {
    let mut event = call_event("inv", "c1");
    event.actions.state_delta.insert("k".into(), json!(1));
    let json = serde_json::to_string(&event).unwrap();
    let back: Event = serde_json::from_str(&json).unwrap();
    assert_eq!(back.invocation_id, "inv");
    assert_eq!(
        serde_json::to_value(&back.llm_response.content).unwrap(),
        serde_json::to_value(&event.llm_response.content).unwrap()
    );
    let response = response_event("inv", "c1", json!({"a": [1, 2.5, "x"]}));
    let back: Event = serde_json::from_str(&serde_json::to_string(&response).unwrap()).unwrap();
    assert_eq!(
        serde_json::to_value(&back.llm_response.content).unwrap(),
        serde_json::to_value(&response.llm_response.content).unwrap()
    );
}

#[tokio::test]
async fn a_thread_keeps_its_nonce_until_a_reseed_and_threads_do_not_share_one() {
    let (_dir, store) = open();
    let sessions = store.store();
    let me = identity(1, "https://a.example");
    let first = sessions.open_session(&me, "fp").await.unwrap().nonce;
    assert_eq!(first.hex().len(), 32);
    assert_eq!(sessions.open_session(&me, "fp").await.unwrap().nonce, first);
    // A changed path policy re-keys the session but is not a re-seed.
    assert_eq!(
        sessions.open_session(&me, "fp2").await.unwrap().nonce,
        first
    );
    let reseeded = sessions
        .seed(&me.key(), Vec::new(), Some("0".into()), &[])
        .await
        .unwrap();
    assert_ne!(reseeded, first);
    assert_eq!(
        sessions.open_session(&me, "fp2").await.unwrap().nonce,
        reseeded
    );
    let other = SessionIdentity {
        conversation_uuid: "aaaaaaaa-2222-4333-8444-555555555555".into(),
        ..me.clone()
    };
    assert_ne!(
        sessions.open_session(&other, "fp").await.unwrap().nonce,
        reseeded
    );
    // Never printed.
    assert!(!format!("{reseeded:?}").contains(&reseeded.hex()));
}

/// #1187: a partial take-back's seed boundary survives a schema-1 store's
/// upgrade, adds up, and goes with the conversation.
#[tokio::test]
async fn the_seed_boundary_is_kept_and_upgraded_from_schema_one() {
    let dir = tempfile::tempdir().unwrap();
    {
        let store = SessionStore::open(dir.path()).unwrap();
        store
            .call_blocking("t", |conn| {
                conn.execute_batch("DROP TABLE seed_skips; PRAGMA user_version = 1;")
            })
            .unwrap();
    }
    let store = SessionStore::open(dir.path()).unwrap();
    let who = identity(7, "https://a.example");
    let key = who.key();
    assert_eq!(store.seed_skips(&key).await.unwrap(), Vec::<i64>::new());
    store
        .skip_on_seed(&who.deployment, 7, &who.conversation_uuid, vec![21, 20])
        .await
        .unwrap();
    store
        .skip_on_seed(&who.deployment, 7, &who.conversation_uuid, vec![20, 31])
        .await
        .unwrap();
    assert_eq!(store.seed_skips(&key).await.unwrap(), [20, 21, 31]);
    let version: i64 = store
        .call_blocking("t", |conn| {
            conn.query_row("PRAGMA user_version", [], |row| row.get(0))
        })
        .unwrap();
    assert_eq!(version, SCHEMA_VERSION);
    store
        .delete_conversation(&who.deployment, 7, &who.conversation_uuid)
        .await
        .unwrap();
    assert_eq!(store.seed_skips(&key).await.unwrap(), Vec::<i64>::new());
}

#[test]
fn one_shrink_rule_for_both_doctor_checks() {
    let mib = 1024 * 1024;
    let space = |bytes: u64, free_fraction: f64, incremental: bool| DbSpace {
        bytes,
        free_fraction,
        incremental,
    };
    assert!(
        space(200 * mib, 0.6, true).shrink_advised(),
        "large and half free"
    );
    assert!(
        !space(200 * mib, 0.1, true).shrink_advised(),
        "large but in use"
    );
    assert!(!space(10 * mib, 0.9, true).shrink_advised(), "small");
    assert!(
        space(80 * mib, 0.0, false).shrink_advised(),
        "too large to switch at open, not yet incremental: the shrink switches it"
    );
    assert!(
        !space(10 * mib, 0.0, false).shrink_advised(),
        "the open switches it"
    );
}

#[tokio::test]
async fn the_doctors_shrink_switches_a_file_to_incremental_vacuum() {
    let dir = tempfile::tempdir().unwrap();
    {
        let store = SessionStore::open(dir.path()).unwrap();
        store
            .open_session(&identity(1, "https://a.example"), "fp")
            .await
            .unwrap();
    }
    // A file too large to switch at open stays in mode NONE; the shrink is
    // what switches it.
    {
        let conn = rusqlite::Connection::open(dir.path().join(FILE_NAME)).unwrap();
        conn.execute_batch("PRAGMA auto_vacuum = NONE; VACUUM;")
            .unwrap();
    }
    assert!(!database_space(dir.path(), FILE_NAME).incremental);
    assert!(reclaim_database(dir.path(), FILE_NAME).unwrap() > 0);
    assert!(database_space(dir.path(), FILE_NAME).incremental);
}

#[tokio::test]
async fn an_idle_marker_joins_the_last_committed_turn_and_survives_a_reopen() {
    let (_dir, state) = open();
    let sessions = state.store();
    let service = state.sessions();
    let me = identity(1, "https://a.example");
    let key = me.key();
    sessions.open_session(&me, "fp").await.unwrap();
    sessions
        .seed(&key, Vec::new(), Some("0".into()), &[])
        .await
        .unwrap();
    let marker = || text_event("i1", "elitea_context_compaction", "user", "marker");
    // An empty session has no committed turn to join.
    assert!(!sessions.append_marker(&key, marker()).await.unwrap());
    let scope = sessions.begin_turn(&key, "exec-1").unwrap();
    service
        .append_event(&key, text_event("i1", "user", "user", "hi"))
        .await
        .unwrap();
    // A turn holds the session: refused, nothing written.
    assert!(!sessions.append_marker(&key, marker()).await.unwrap());
    sessions
        .finish_turn(
            &key,
            "exec-1",
            TurnOutcome::Committed {
                response_message_id: "resp-1".into(),
                cursor: Some("2".into()),
            },
        )
        .await
        .unwrap();
    drop(scope);
    assert!(sessions.append_marker(&key, marker()).await.unwrap());
    let rows = sessions.rows(&key).await.unwrap();
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[1].turn_execution_id, "exec-1");
    assert_eq!(rows[1].response_message_id.as_deref(), Some("resp-1"));
    // A reopen keeps it: it is a committed turn's event.
    assert_eq!(sessions.open_session(&me, "fp").await.unwrap().events, 2);
}
